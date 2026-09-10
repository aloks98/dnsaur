import { delay, http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { makeQueryClient } from "../lib/query-client";
import type { ApiToken, MeResponse } from "../api/types";
import { Account } from "./account";

const TOTP_SECRET = "JBSWY3DPEHPK3PXP";
// The QR is drawn server-side (internal/api/tokens_handlers.go) and arrives
// base64-encoded; a 1x1 PNG stands in for it here. jsdom never decodes it —
// what matters is that the page puts it straight into the img's src.
const TOTP_QR_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";

/**
 * renderWithProviders() owns its QueryClient privately; this variant hands
 * it back, because "the secret is no longer rendered" and "the secret is
 * gone" are different claims and only the second one is the fix. A
 * useMutation keeps both its `.data` and the `variables` it was called with
 * until an explicit reset(), so the only way to prove the reset happened is
 * to read the mutation cache.
 *
 * `mutations.gcTime: 0` is not the production value. It only shortens the
 * window between "this mutation has no observers left" (which is exactly
 * what reset() produces) and query-core evicting it, from five minutes to
 * the next tick, so the assertion is observable inside a test. Without the
 * reset the dialog's observer stays subscribed for the whole page session,
 * nothing becomes collectable, and the assertion fails — which is the
 * regression this guards.
 */
function renderWithQueryClient(ui: ReactElement): { client: QueryClient } {
  const client = makeQueryClient();
  const defaults = client.getDefaultOptions();
  client.setDefaultOptions({ ...defaults, mutations: { ...defaults.mutations, gcTime: 0 } });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
  return { client };
}

/** Every value any mutation on the page is still holding onto — results and
 * the variables they were called with alike. */
function mutationStateJson(client: QueryClient): string {
  return JSON.stringify(
    client
      .getMutationCache()
      .getAll()
      .map((m) => ({ variables: m.state.variables, data: m.state.data })),
  );
}

function mockMe(overrides: Partial<MeResponse> = {}) {
  const me: MeResponse = { id: 1, username: "admin", totp_enabled: false, ...overrides };
  server.use(http.get("/api/v1/auth/me", () => HttpResponse.json(me)));
}

function mockTokens(tokens: ApiToken[]) {
  server.use(http.get("/api/v1/tokens", () => HttpResponse.json(tokens)));
}

function sampleToken(overrides: Partial<ApiToken> = {}): ApiToken {
  return {
    id: 1,
    user_id: 1,
    kind: "api",
    name: "Home Assistant",
    scope: "read",
    created_at: Date.now() - 5 * 24 * 60 * 60 * 1000,
    // Every API token is non-expiring; only sessions carry an expiry.
    expires_at: 0,
    last_used: 0,
    ...overrides,
  };
}

test("shows a loading skeleton, then both sections, once account data arrives", async () => {
  server.use(
    http.get("/api/v1/auth/me", async () => {
      await delay(30);
      return HttpResponse.json({ id: 1, username: "admin", totp_enabled: false });
    }),
  );
  mockTokens([]);

  renderWithProviders(<Account />);

  expect(screen.queryByText("Two-factor authentication")).not.toBeInTheDocument();
  expect(await screen.findByText("Two-factor authentication")).toBeInTheDocument();
  expect(screen.getByText("API tokens")).toBeInTheDocument();
});

test("shows an alert when the account fails to load", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderWithProviders(<Account />);

  expect(
    await screen.findByText(/couldn't load your account/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
});

test("says password changes aren't available yet, rather than showing a non-functional form", async () => {
  mockMe();
  mockTokens([]);

  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");

  expect(screen.getByText(/password changes aren.t available yet/i)).toBeInTheDocument();
  expect(screen.queryByLabelText(/current password/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/new password/i)).not.toBeInTheDocument();
});

test("TOTP off shows Enable 2FA; TOTP on shows Disable 2FA and an Enabled badge", async () => {
  mockMe({ totp_enabled: false });
  mockTokens([]);
  const off = renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");
  expect(screen.getByRole("button", { name: /^set up 2fa$/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^disable 2fa$/i })).not.toBeInTheDocument();
  expect(screen.getByText(/^disabled$/i)).toBeInTheDocument();
  off.unmount();

  mockMe({ totp_enabled: true });
  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");
  expect(screen.getByRole("button", { name: /^disable 2fa$/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^set up 2fa$/i })).not.toBeInTheDocument();
  expect(screen.getByText(/^enabled$/i)).toBeInTheDocument();
});

// Required test (c): start -> confirm with a code -> `me` refetches and the
// card reflects the newly-enabled state.
test("TOTP enable: start then confirm with a code refetches me and flips the card to enabled", async () => {
  const user = userEvent.setup();
  let totpEnabled = false;
  let confirmBody: unknown;
  server.use(
    http.get("/api/v1/auth/me", () =>
      HttpResponse.json({ id: 1, username: "admin", totp_enabled: totpEnabled }),
    ),
    http.post("/api/v1/auth/totp/start", () =>
      HttpResponse.json({
        secret: "JBSWY3DPEHPK3PXP",
        otpauth_url: "otpauth://totp/dnsaur:admin?secret=JBSWY3DPEHPK3PXP&issuer=dnsaur",
        qr_png: TOTP_QR_PNG,
      }),
    ),
    http.post("/api/v1/auth/totp/confirm", async ({ request }) => {
      confirmBody = await request.json();
      totpEnabled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  mockTokens([]);
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");

  await user.click(screen.getByRole("button", { name: /^set up 2fa$/i }));

  // Enrollment happens in the section itself, not a dialog — the QR, the
  // secret and the confirm field are all on the page at once.
  expect(await screen.findByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();
  // The QR is whatever the server drew, shown as-is — nothing in the bundle
  // re-encodes the otpauth:// URL.
  expect(await screen.findByRole("img", { name: /qr code/i })).toHaveAttribute(
    "src",
    `data:image/png;base64,${TOTP_QR_PNG}`,
  );
  expect(screen.getByText(/setting up/i)).toBeInTheDocument();

  await user.type(screen.getByLabelText(/verification code/i), "123456");
  await user.click(screen.getByRole("button", { name: /^turn on 2fa$/i }));

  await waitFor(() => expect(confirmBody).toEqual({ secret: "JBSWY3DPEHPK3PXP", code: "123456" }));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Two-factor authentication enabled"));

  // me was invalidated and refetched — the card now shows the enabled state.
  expect(await screen.findByRole("button", { name: /^disable 2fa$/i })).toBeInTheDocument();
});

// Symmetric to the reveal-dialog regression guard above: dismissing the
// enable dialog without confirming (Cancel) must reset totpStart (see
// TotpCard.onEnableOpenChange), not just the local `enrollment` state —
// the setup key and QR must be gone from the DOM, and starting over must
// not resurrect the old secret.
test("TOTP enable: dismissing without confirming clears the setup key and QR from the DOM", async () => {
  const user = userEvent.setup();
  mockMe({ totp_enabled: false });
  server.use(
    http.post("/api/v1/auth/totp/start", () =>
      HttpResponse.json({
        secret: "JBSWY3DPEHPK3PXP",
        otpauth_url: "otpauth://totp/dnsaur:admin?secret=JBSWY3DPEHPK3PXP&issuer=dnsaur",
        qr_png: TOTP_QR_PNG,
      }),
    ),
  );
  mockTokens([]);

  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");

  await user.click(screen.getByRole("button", { name: /^set up 2fa$/i }));
  expect(await screen.findByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();
  expect(await screen.findByRole("img", { name: /qr code/i })).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^cancel$/i }));

  await waitFor(() => expect(screen.queryByText("JBSWY3DPEHPK3PXP")).not.toBeInTheDocument());
  expect(screen.queryByRole("img", { name: /qr code/i })).not.toBeInTheDocument();
  // Still off — nothing was confirmed.
  expect(screen.getByRole("button", { name: /^set up 2fa$/i })).toBeInTheDocument();
});

// The leak the DOM assertions above can't see. TotpEnableDialog is rendered
// unconditionally by TotpCard, so it never unmounts — and `mutate({ secret,
// code })` parks the shared secret in the confirm mutation's `variables`,
// which query-core carries through the "success" action untouched. Before
// TotpCard owned (and reset) that mutation, the live shared secret stayed
// readable via getMutationCache() and React DevTools for the rest of the
// page session, right after enrollment succeeded.
test("TOTP enable: the shared secret is gone from mutation state once enrollment finishes", async () => {
  const user = userEvent.setup();
  let totpEnabled = false;
  server.use(
    http.get("/api/v1/auth/me", () =>
      HttpResponse.json({ id: 1, username: "admin", totp_enabled: totpEnabled }),
    ),
    http.post("/api/v1/auth/totp/start", () =>
      HttpResponse.json({
        secret: TOTP_SECRET,
        otpauth_url: `otpauth://totp/dnsaur:admin?secret=${TOTP_SECRET}&issuer=dnsaur`,
        qr_png: TOTP_QR_PNG,
      }),
    ),
    http.post("/api/v1/auth/totp/confirm", () => {
      totpEnabled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  mockTokens([]);

  const { client } = renderWithQueryClient(<Account />);
  await screen.findByText("Two-factor authentication");

  await user.click(screen.getByRole("button", { name: /^set up 2fa$/i }));
  await screen.findByText(TOTP_SECRET);
  // Sanity check: while the flow is open the secret genuinely is in
  // mutation state, so the assertion below is testing something.
  expect(mutationStateJson(client)).toContain(TOTP_SECRET);

  await user.type(screen.getByLabelText(/verification code/i), "123456");
  await user.click(screen.getByRole("button", { name: /^turn on 2fa$/i }));

  await waitFor(() => expect(mutationStateJson(client)).not.toContain(TOTP_SECRET));
  expect(screen.queryByText(TOTP_SECRET)).not.toBeInTheDocument();
});

test("TOTP disable: a valid code disables 2FA, me refetches, and the code doesn't linger", async () => {
  const user = userEvent.setup();
  let totpEnabled = true;
  let disableBody: unknown;
  server.use(
    http.get("/api/v1/auth/me", () =>
      HttpResponse.json({ id: 1, username: "admin", totp_enabled: totpEnabled }),
    ),
    http.post("/api/v1/auth/totp/disable", async ({ request }) => {
      disableBody = await request.json();
      totpEnabled = false;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  mockTokens([]);

  const { client } = renderWithQueryClient(<Account />);
  await screen.findByText("Two-factor authentication");
  await user.click(screen.getByRole("button", { name: /^disable 2fa$/i }));

  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/verification code/i), "654321");
  await user.click(within(dialog).getByRole("button", { name: /^disable 2fa$/i }));

  await waitFor(() => expect(disableBody).toEqual({ code: "654321" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(await screen.findByRole("button", { name: /^set up 2fa$/i })).toBeInTheDocument();
  // Lower stakes than the shared secret (single-use, time-limited) but the
  // same lingering-variables leak, closed the same way.
  await waitFor(() => expect(mutationStateJson(client)).not.toContain("654321"));
});

test("TOTP disable: an invalid code shows an inline error and keeps the dialog open", async () => {
  const user = userEvent.setup();
  mockMe({ totp_enabled: true });
  server.use(
    http.post("/api/v1/auth/totp/disable", () =>
      HttpResponse.json({ error: "invalid code" }, { status: 400 }),
    ),
  );
  mockTokens([]);

  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");
  await user.click(screen.getByRole("button", { name: /^disable 2fa$/i }));

  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/verification code/i), "000000");
  await user.click(within(dialog).getByRole("button", { name: /^disable 2fa$/i }));

  expect(await within(dialog).findByText(/invalid code/i)).toBeInTheDocument();
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

test("shows an EmptyState with a New token action when there are no tokens", async () => {
  mockMe();
  mockTokens([]);

  renderWithProviders(<Account />);

  expect(await screen.findByText("No API tokens yet")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /new token/i })).toBeInTheDocument();
});

test("the empty state gives way to the create row rather than sitting under it", async () => {
  mockMe();
  mockTokens([]);

  const user = userEvent.setup();
  renderWithProviders(<Account />);
  await screen.findByText(/no api tokens yet/i);
  await user.click(screen.getByRole("button", { name: /new token/i }));

  expect(document.querySelector('[data-slot="new-token-row"]')).toBeInTheDocument();
  // Both halves of the empty state are now false: there is something
  // here, and the way to start is already open.
  expect(screen.queryByText("No API tokens yet")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /new token/i })).not.toBeInTheDocument();
});

test("renders the tokens table with name, scope badge, and relative created/last-used times", async () => {
  mockMe();
  mockTokens([
    sampleToken({ id: 1, name: "Home Assistant", scope: "read", last_used: Date.now() - 60_000 }),
    sampleToken({ id: 2, name: "CI script", scope: "write", last_used: 0 }),
  ]);

  renderWithProviders(<Account />);
  await screen.findByText("Home Assistant");

  expect(screen.getByText("Read-only")).toBeInTheDocument();
  expect(screen.getByText("Read & write")).toBeInTheDocument();
  expect(screen.getByText("CI script")).toBeInTheDocument();

  // last_used: 0 reads as "never" (relativeTime's sentinel), not an epoch
  // date. Scoped to the row: the grid also has an Expires column, which
  // reads "never" for every token since API tokens do not expire.
  const rows = document.querySelectorAll<HTMLElement>('[data-slot="token-row"]');
  expect(within(rows[1]).getAllByText("never").length).toBeGreaterThanOrEqual(1);
});

// Required test (d): the API omits token_hash, and the table must never
// render it even if a future server bug leaks it into the response.
test("the tokens table never renders a token_hash, even if the server response includes one", async () => {
  mockMe();
  server.use(
    http.get("/api/v1/tokens", () =>
      HttpResponse.json([
        {
          id: 1,
          user_id: 1,
          kind: "api",
          name: "Home Assistant",
          token_hash: "sha256:super-secret-hash-should-never-render",
          scope: "read",
          created_at: Date.now(),
          expires_at: 0,
          last_used: 0,
        },
      ]),
    ),
  );

  renderWithProviders(<Account />);
  await screen.findByText("Home Assistant");

  expect(screen.queryByText(/super-secret-hash-should-never-render/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/token_hash/i)).not.toBeInTheDocument();
});

// The row's scope control is a native select carrying the stored values —
// "read" and "write" are what the API takes and what the row's badge shows,
// so a prettier label here would be a third name for the same thing.
test("the new-token row posts the scope that was picked", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockMe();
  mockTokens([]);
  server.use(
    http.post("/api/v1/tokens", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1, token: "dnsaur_pat_abcdef123456" }, { status: 201 });
    }),
  );

  renderWithProviders(<Account />);
  await screen.findByText(/no api tokens yet/i);
  await user.click(screen.getByRole("button", { name: /new token/i }));

  const row = document.querySelector<HTMLElement>('[data-slot="new-token-row"]')!;
  await user.type(within(row).getByLabelText(/^name$/i), "grafana");
  await user.selectOptions(within(row).getByLabelText(/^scope$/i), "write");
  await user.click(within(row).getByRole("button", { name: /^create$/i }));

  await waitFor(() => expect(body).toEqual({ name: "grafana", scope: "write" }));
});

test("creating a token reveals the plaintext exactly once, then it's gone", async () => {
  const user = userEvent.setup();
  mockMe();
  let tokensState: ApiToken[] = [];
  let createBody: unknown;
  server.use(
    http.get("/api/v1/tokens", () => HttpResponse.json(tokensState)),
    http.post("/api/v1/tokens", async ({ request }) => {
      createBody = await request.json();
      const created = sampleToken({ id: 7, name: "Home Assistant", scope: "read" });
      tokensState = [...tokensState, created];
      return HttpResponse.json({ id: 7, token: "dnsaur_pat_abcdef123456" }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Account />);
  await screen.findByText("No API tokens yet");

  await user.click(screen.getByRole("button", { name: /new token/i }));
  const createDialog = document.querySelector<HTMLElement>('[data-slot="new-token-row"]')!;
  await user.type(within(createDialog).getByLabelText(/^name$/i), "Home Assistant");
  await user.click(within(createDialog).getByRole("button", { name: /^create$/i }));

  await waitFor(() => expect(createBody).toEqual({ name: "Home Assistant", scope: "read" }));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Token created"));

  // A banner, not a dialog: this is the only time the plaintext exists on
  // screen, and a modal invites the two gestures that lose it — Escape and
  // a backdrop click. It goes only when its own button is pressed.
  expect(await screen.findByText(/copy your token now/i)).toBeInTheDocument();
  expect(screen.getByText("dnsaur_pat_abcdef123456")).toBeInTheDocument();

  await user.keyboard("{Escape}");
  expect(screen.getByText("dnsaur_pat_abcdef123456")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /i.ve saved it/i }));

  await waitFor(() =>
    expect(screen.queryByText("dnsaur_pat_abcdef123456")).not.toBeInTheDocument(),
  );

  // The row now exists, but only its name/scope show — never the plaintext.
  expect(await screen.findByText("Home Assistant")).toBeInTheDocument();
  expect(screen.queryByText("dnsaur_pat_abcdef123456")).not.toBeInTheDocument();

  // Regression guard for the mutation-data-lingers finding: dismissing the
  // reveal dialog resets createToken (see TokensCard.onDismissReveal), not
  // just the local `revealResult` state that gates rendering it. Reopening
  // "New token" afterward must not surface the earlier plaintext anywhere.
  await user.click(screen.getByRole("button", { name: /new token/i }));
  expect(await screen.findByLabelText(/^name$/i)).toBeInTheDocument();
  expect(screen.queryByText("dnsaur_pat_abcdef123456")).not.toBeInTheDocument();
});

/** Reveal a freshly created token and hand back the banner's Copy button. */
async function revealToken(user: ReturnType<typeof userEvent.setup>) {
  mockMe();
  server.use(
    http.get("/api/v1/tokens", () => HttpResponse.json([])),
    http.post("/api/v1/tokens", () =>
      HttpResponse.json({ id: 7, token: "dnsaur_pat_abcdef123456" }, { status: 201 }),
    ),
  );

  renderWithProviders(<Account />);
  await screen.findByText("No API tokens yet");
  await user.click(screen.getByRole("button", { name: /new token/i }));
  const row = document.querySelector<HTMLElement>('[data-slot="new-token-row"]')!;
  await user.type(within(row).getByLabelText(/^name$/i), "Home Assistant");
  await user.click(within(row).getByRole("button", { name: /^create$/i }));

  await screen.findByText("dnsaur_pat_abcdef123456");
  return screen.getByRole("button", { name: /copy token/i });
}

// `navigator.clipboard` is undefined outside a secure context, which is
// exactly where a homelab instance reached over plain HTTP lives. The button
// toasted success unconditionally, so on http://192.168.1.2 the operator saw
// "Token copied", pressed "I've saved it", and the token was gone.
test("a clipboard the browser won't hand over is reported, not toasted as success", async () => {
  const user = userEvent.setup();
  const successSpy = vi.spyOn(toast, "success");
  const errorSpy = vi.spyOn(toast, "error");
  vi.spyOn(navigator, "clipboard", "get").mockReturnValue(
    undefined as unknown as Navigator["clipboard"],
  );

  await user.click(await revealToken(user));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/couldn't/i)));
  expect(successSpy).not.toHaveBeenCalledWith("Token copied");
  // And the token is still on screen to copy by hand.
  expect(screen.getByText("dnsaur_pat_abcdef123456")).toBeInTheDocument();
});

test("a clipboard write that fails is reported too, not assumed to have worked", async () => {
  const user = userEvent.setup();
  const successSpy = vi.spyOn(toast, "success");
  const errorSpy = vi.spyOn(toast, "error");
  vi.spyOn(navigator, "clipboard", "get").mockReturnValue({
    writeText: () => Promise.reject(new Error("denied")),
  } as unknown as Navigator["clipboard"]);

  await user.click(await revealToken(user));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/couldn't/i)));
  expect(successSpy).not.toHaveBeenCalledWith("Token copied");
});

test("a clipboard write that lands says so", async () => {
  const user = userEvent.setup();
  const successSpy = vi.spyOn(toast, "success");
  const written: string[] = [];
  vi.spyOn(navigator, "clipboard", "get").mockReturnValue({
    writeText: (text: string) => {
      written.push(text);
      return Promise.resolve();
    },
  } as unknown as Navigator["clipboard"]);

  await user.click(await revealToken(user));

  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Token copied"));
  expect(written).toEqual(["dnsaur_pat_abcdef123456"]);
});

// TOKEN_GRID and the Section band are fixed-pixel column templates inside a
// shell that is h-screen/overflow-hidden (components/app-shell.tsx), so a
// narrow viewport clipped the Revoke column with nothing to scroll.
test("the fixed-width token grid sits in a horizontal scroll container", async () => {
  mockMe();
  mockTokens([sampleToken({ id: 3, name: "CI script" })]);

  renderWithProviders(<Account />);
  await screen.findByText("CI script");

  const scroller = document.querySelector('[data-slot="h-scroll"]');
  expect(scroller).not.toBeNull();
  expect(scroller!.className).toContain("overflow-x-auto");
  expect(scroller!.contains(screen.getByText("CI script"))).toBe(true);
});

// Required test (b): revoke DELETEs the token and its row disappears.
test("revoking a token DELETEs it and the row disappears", async () => {
  const user = userEvent.setup();
  mockMe();
  let tokensState: ApiToken[] = [sampleToken({ id: 3, name: "CI script" })];
  let deletedId: number | null = null;
  server.use(
    http.get("/api/v1/tokens", () => HttpResponse.json(tokensState)),
    http.delete("/api/v1/tokens/:id", ({ params }) => {
      deletedId = Number(params.id);
      tokensState = tokensState.filter((t) => t.id !== deletedId);
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Account />);
  await screen.findByText("CI script");

  await user.click(screen.getByRole("button", { name: /revoke ci script/i }));
  // The revoke confirmation is an AlertDialog (role="alertdialog"), not a
  // plain Dialog — distinct from the create/enable/disable dialogs below.
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).getByText(/revoke this token/i)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: /^revoke$/i }));

  await waitFor(() => expect(deletedId).toBe(3));
  await waitFor(() => expect(screen.queryByText("CI script")).not.toBeInTheDocument());
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Token revoked"));
  expect(await screen.findByText("No API tokens yet")).toBeInTheDocument();
});

test("a failed token creation surfaces the server's error as a toast and leaves the row open", async () => {
  const user = userEvent.setup();
  mockMe();
  mockTokens([]);
  server.use(
    http.post("/api/v1/tokens", () =>
      HttpResponse.json({ error: "name required" }, { status: 400 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Account />);
  await screen.findByText("No API tokens yet");
  await user.click(screen.getByRole("button", { name: /new token/i }));
  const row = document.querySelector<HTMLElement>('[data-slot="new-token-row"]')!;
  await user.type(within(row).getByLabelText(/^name$/i), "Anything");
  await user.click(within(row).getByRole("button", { name: /^create$/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("name required"));
  // The row stays put with the typed name intact, so it can be corrected
  // rather than retyped.
  expect(document.querySelector('[data-slot="new-token-row"]')).toBeInTheDocument();
  expect(within(row).getByLabelText(/^name$/i)).toHaveValue("Anything");
});
