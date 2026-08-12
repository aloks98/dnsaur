import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { TSIGKey } from "../api/types";
import { MASK, TSIGKeys } from "./tsig-keys";

// A real 32-byte base64 secret, the shape the generator produces and the
// only shape the API accepts (it base64-decodes at write time).
const SECRET_256 = "xb10GdOUbvmqpzu1X6pbQvdmcK0Kkp2XnMKZJt/k5ZI=";
const SECRET_512 = "KrE5LmHD5Jb7wbs7qHisdwUX466I/RY5Kfj40nppDvk=";
const SECRET_OLD = "MHmDwZnh8rYuvii/VpkjOFUwrQdJgxH3xGO8ofvAItw=";

function key(overrides: Partial<TSIGKey> = {}): TSIGKey {
  return {
    id: 1,
    name: "xfer.e412.in.",
    // The wire value carries the trailing dot — these are miekg's own
    // constants (internal/api/tsigkeys_handlers.go's tsigAlgorithms).
    algorithm: "hmac-sha256.",
    secret: SECRET_256,
    created_at: Date.now() - 86_400_000,
    ...overrides,
  };
}

function mockKeys(keys: TSIGKey[]) {
  server.use(http.get("/api/v1/tsig-keys", () => HttpResponse.json(keys)));
}

function rows(): HTMLElement[] {
  return screen.getAllByTestId("tsig-key-row");
}

async function openCreateRow(user: ReturnType<typeof userEvent.setup>) {
  // findAllBy*: an empty list offers "New key" twice (header + empty state).
  const [button] = await screen.findAllByRole("button", { name: /new key/i });
  await user.click(button);
}

afterEach(() => vi.restoreAllMocks());

// The screen's central display decision: four secrets are not left sitting
// in plain sight, and only one can be open at a time — revealing the second
// row closes the first, so a screen share never accumulates them.
test("secrets are masked at rest and the eye reveals one row at a time", async () => {
  const user = userEvent.setup();
  mockKeys([
    key({ id: 1, name: "xfer.e412.in.", secret: SECRET_256 }),
    key({ id: 2, name: "secondary.e412.in.", algorithm: "hmac-sha512.", secret: SECRET_512 }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  // Masked to begin with — neither secret is anywhere on the page.
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /show the secret for xfer\.e412\.in\./i }));
  expect(within(rows()[0]).getByText(SECRET_256)).toBeInTheDocument();
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();

  // Opening the second closes the first, rather than adding to it.
  await user.click(
    screen.getByRole("button", { name: /show the secret for secondary\.e412\.in\./i }),
  );
  expect(within(rows()[1]).getByText(SECRET_512)).toBeInTheDocument();
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();

  // And the open one closes itself.
  await user.click(
    screen.getByRole("button", { name: /hide the secret for secondary\.e412\.in\./i }),
  );
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();
});

// Copying is the common task (the secret has to be pasted into the peer's
// config verbatim); reading it aloud is the exception. So Copy is always
// available and does NOT reveal — the row stays masked through the click.
test("Copy puts the real secret on the clipboard while the row stays masked", async () => {
  const user = userEvent.setup();
  const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.resolve());
  vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
  mockKeys([key({ id: 1, name: "xfer.e412.in.", secret: SECRET_256 })]);

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /copy the secret for xfer\.e412\.in\./i }));

  await waitFor(() => expect(writeText).toHaveBeenCalledWith(SECRET_256));
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();
});

// Generating is the default path — a hand-typed secret is usually a weak
// one. What matters is that the value the admin was shown is the value that
// reaches the server: the field is the record of what was generated, and
// anything else would put a key on the peer that dnsaur never stored.
test("the create row posts the generated secret exactly as displayed", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  const generated = secret.value;
  // 32 bytes, base64 — 44 characters with one pad byte.
  expect(generated).toMatch(/^[A-Za-z0-9+/]{43}=$/);

  await user.type(screen.getByLabelText(/key name/i), "  XFER.e412.IN  ");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(body).toEqual({
      // Trimmed but not canonicalised here — the server lowercases and adds
      // the trailing dot, and the row said so before the click.
      name: "XFER.e412.IN",
      algorithm: "hmac-sha256.",
      secret: generated,
    }),
  );
});

// The mismatch this screen exists on top of: the API's algorithm values are
// miekg's constants and carry a trailing dot; the design shows them without
// one. If the select's values were the *displayed* strings, no fetched key
// would ever match an option — the control would show the first algorithm
// instead, and saving an untouched row would silently rewrite hmac-sha512 to
// hmac-sha1.
test("the algorithm select round-trips a fetched value rather than rewriting it", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockKeys([key({ id: 4, name: "secondary.e412.in.", algorithm: "hmac-sha512." })]);
  server.use(
    http.put("/api/v1/tsig-keys/4", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));
  // Displayed without the dot, at rest.
  expect(within(rows()[0]).getByText("hmac-sha512")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /edit secondary\.e412\.in\./i }));

  const select = screen.getByLabelText(/algorithm/i) as HTMLSelectElement;
  expect(select.value).toBe("hmac-sha512.");
  // The option is *labelled* without the dot and *valued* with it.
  expect(within(select).getByRole("option", { name: "hmac-sha512" })).toHaveProperty(
    "selected",
    true,
  );
  expect(within(select).queryByRole("option", { name: "hmac-sha512." })).not.toBeInTheDocument();

  // Saving an untouched row must put back exactly what came out.
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await waitFor(() =>
    expect(body).toEqual({
      name: "secondary.e412.in.",
      algorithm: "hmac-sha512.",
      secret: SECRET_256,
    }),
  );
});

test("deleting a key confirms inline, then DELETEs it and drops the row", async () => {
  const user = userEvent.setup();
  let deleted = false;
  const keys = [
    key({ id: 1, name: "xfer.e412.in." }),
    key({ id: 7, name: "legacy-xfer.e412.in.", algorithm: "hmac-sha1.", secret: SECRET_OLD }),
  ];
  server.use(
    http.get("/api/v1/tsig-keys", () =>
      HttpResponse.json(deleted ? keys.filter((k) => k.id !== 7) : keys),
    ),
    http.delete("/api/v1/tsig-keys/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  expect(await screen.findByText("Delete legacy-xfer.e412.in.?")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(rows()).toHaveLength(1));
  expect(screen.queryByText("legacy-xfer.e412.in.")).not.toBeInTheDocument();
});

test("the delete confirm can be cancelled and nothing is sent", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockKeys([key({ id: 7, name: "legacy-xfer.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  await user.click(await screen.findByRole("button", { name: /^cancel$/i }));

  await waitFor(() =>
    expect(screen.queryByText("Delete legacy-xfer.e412.in.?")).not.toBeInTheDocument(),
  );
  expect(deleted).toBe(false);
});

// The server canonicalises every name it is given (lowercase, trailing dot
// — normalizeTSIGName in tsigkeys_handlers.go), and the key has to match the
// name the peer signs with. Saying so while it is being typed is cheaper
// than showing a row that doesn't look like what was entered.
test("the create row shows what the typed name will be saved as", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  // Nothing typed, nothing claimed.
  expect(screen.queryByText(/^saved as/i)).not.toBeInTheDocument();

  await user.type(screen.getByLabelText(/key name/i), "XFER.e412.IN");
  expect(await screen.findByText("Saved as xfer.e412.in.")).toBeInTheDocument();
});

// The other half of the create row's secret story: generate is the default,
// and "Paste an existing secret" is the deliberate departure from it —
// which has to clear the generated value, or the field silently keeps a
// secret the peer has never heard of.
test("switching to paste clears the generated secret, and generating again refills it", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  expect(secret.value).not.toBe("");
  expect(screen.getByText("base64, 32 bytes")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /paste an existing secret/i }));
  expect(secret.value).toBe("");
  expect(screen.getByPlaceholderText(/base64 secret from the other server/i)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /generate one/i }));
  expect(secret.value).toMatch(/^[A-Za-z0-9+/]{43}=$/);
});

test("regenerate replaces the secret with a different one", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  const first = secret.value;
  await user.click(screen.getByRole("button", { name: /generate a new secret/i }));

  expect(secret.value).not.toBe(first);
  expect(secret.value).toMatch(/^[A-Za-z0-9+/]{43}=$/);
});

test("a name the server would refuse is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/key name/i), "has spaces.e412.in");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/enter a domain name/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("a secret that isn't base64 is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.click(screen.getByRole("button", { name: /paste an existing secret/i }));
  await user.type(screen.getByLabelText(/key name/i), "xfer.e412.in");
  await user.type(screen.getByLabelText(/^secret$/i), "not base64!!");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/base64/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("shows an empty state with the artboard's copy and a New key action", async () => {
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);

  expect(await screen.findByText("No TSIG keys yet.")).toBeInTheDocument();
  expect(screen.getByText("No keys")).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /new key/i }).length).toBeGreaterThan(0);
});

test("the header counts keys and pluralises", async () => {
  mockKeys([key({ id: 1 })]);
  const one = renderWithProviders(<TSIGKeys />);
  expect(await screen.findByText("1 key")).toBeInTheDocument();
  one.unmount();

  mockKeys([key({ id: 1 }), key({ id: 2, name: "b.e412.in." })]);
  renderWithProviders(<TSIGKeys />);
  expect(await screen.findByText("2 keys")).toBeInTheDocument();
});

// Milestone D2's work, deliberately not built: nothing reads zones.tsig_key_id
// yet, so a USED BY column would be empty on every row forever and the
// in-use delete guard could never fire. Pinned so the omission stays a
// decision rather than becoming an oversight.
test("there is no USED BY column while nothing references a key", async () => {
  mockKeys([key({ id: 1 })]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  expect(screen.queryByText(/used by/i)).not.toBeInTheDocument();
  expect(screen.getByText("Name")).toBeInTheDocument();
  expect(screen.getByText("Algorithm")).toBeInTheDocument();
  expect(screen.getByText("Secret")).toBeInTheDocument();
  expect(screen.getByText("Actions")).toBeInTheDocument();
});

test("a failed delete shows a toast and leaves the key in the list", async () => {
  const user = userEvent.setup();
  mockKeys([key({ id: 7, name: "legacy-xfer.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/7", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  await user.click(await screen.findByRole("button", { name: /^delete$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringMatching(/couldn't delete legacy-xfer\.e412\.in\./i),
    ),
  );
  expect(rows()).toHaveLength(1);
});

test("a rejected create keeps the row open and reports the server's reason", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () =>
      HttpResponse.json({ error: "a TSIG key with that name already exists" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/key name/i), "xfer.e412.in");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith("a TSIG key with that name already exists"),
  );
  expect(screen.getByLabelText(/key name/i)).toBeInTheDocument();
});

// A first load that failed has nothing to fall back to, so it says so
// instead of rendering an empty list that reads as "you have no keys".
test("a failed load says so rather than showing an empty list", async () => {
  server.use(
    http.get("/api/v1/tsig-keys", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  renderWithProviders(<TSIGKeys />);

  // `timeout` because the query client retries once with a backoff before
  // the failure is final (see lib/query-client.ts).
  expect(
    await screen.findByText(/couldn't load tsig keys/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
  expect(screen.queryByText("No TSIG keys yet.")).not.toBeInTheDocument();
});
