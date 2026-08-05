import { delay, http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { ApiToken, MeResponse } from "../api/types";
import { Account } from "./account";

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
    name: "Home Assistant",
    scope: "read",
    created_at: Date.now() - 5 * 24 * 60 * 60 * 1000,
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
  expect(screen.getByRole("button", { name: /^enable 2fa$/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^disable 2fa$/i })).not.toBeInTheDocument();
  expect(screen.getByText(/^disabled$/i)).toBeInTheDocument();
  off.unmount();

  mockMe({ totp_enabled: true });
  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");
  expect(screen.getByRole("button", { name: /^disable 2fa$/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^enable 2fa$/i })).not.toBeInTheDocument();
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

  await user.click(screen.getByRole("button", { name: /^enable 2fa$/i }));

  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(/set up two-factor authentication/i)).toBeInTheDocument();
  // The setup key is shown for manual entry, and the QR is rendered from it.
  expect(within(dialog).getByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();
  expect(await within(dialog).findByRole("img", { name: /qr code/i })).toBeInTheDocument();

  await user.type(within(dialog).getByLabelText(/verification code/i), "123456");
  await user.click(within(dialog).getByRole("button", { name: /^confirm$/i }));

  await waitFor(() => expect(confirmBody).toEqual({ secret: "JBSWY3DPEHPK3PXP", code: "123456" }));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Two-factor authentication enabled"));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

  // me was invalidated and refetched — the card now shows the enabled state.
  expect(await screen.findByRole("button", { name: /^disable 2fa$/i })).toBeInTheDocument();
});

test("TOTP disable: a valid code disables 2FA and me refetches", async () => {
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

  renderWithProviders(<Account />);
  await screen.findByText("Two-factor authentication");
  await user.click(screen.getByRole("button", { name: /^disable 2fa$/i }));

  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/verification code/i), "654321");
  await user.click(within(dialog).getByRole("button", { name: /^disable 2fa$/i }));

  await waitFor(() => expect(disableBody).toEqual({ code: "654321" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(await screen.findByRole("button", { name: /^enable 2fa$/i })).toBeInTheDocument();
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
  // last_used: 0 reads as "never" (relativeTime's sentinel), not an epoch date.
  expect(screen.getByText("never")).toBeInTheDocument();
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

test("the New token Scope select shows human labels, not raw values", async () => {
  const user = userEvent.setup();
  mockMe();
  mockTokens([]);

  renderWithProviders(<Account />);
  await screen.findByText("No API tokens yet");
  await user.click(screen.getByRole("button", { name: /new token/i }));

  const dialog = await screen.findByRole("dialog");
  const scopeTrigger = within(dialog).getByRole("combobox", { name: /^scope$/i });
  expect(scopeTrigger).toHaveTextContent(/read-only/i);

  await user.click(scopeTrigger);
  await user.click(screen.getByRole("option", { name: /read & write/i }));
  expect(scopeTrigger).toHaveTextContent(/read & write/i);
});

// Required test (a): the plaintext token is shown exactly once, then gone
// for good once the reveal dialog is dismissed.
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
  const createDialog = await screen.findByRole("dialog");
  await user.type(within(createDialog).getByLabelText(/^name$/i), "Home Assistant");
  await user.click(within(createDialog).getByRole("button", { name: /^create token$/i }));

  await waitFor(() => expect(createBody).toEqual({ name: "Home Assistant", scope: "read" }));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Token created"));

  const revealDialog = await screen.findByRole("dialog", { name: /copy your token now/i });
  expect(within(revealDialog).getByText("dnsaur_pat_abcdef123456")).toBeInTheDocument();

  await user.click(within(revealDialog).getByRole("button", { name: /i.ve saved it/i }));

  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(screen.queryByText("dnsaur_pat_abcdef123456")).not.toBeInTheDocument();

  // The row now exists, but only its name/scope show — never the plaintext.
  expect(await screen.findByText("Home Assistant")).toBeInTheDocument();
  expect(screen.queryByText("dnsaur_pat_abcdef123456")).not.toBeInTheDocument();
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

test("a failed token creation surfaces the server's error as a toast and leaves the dialog open", async () => {
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
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "Anything");
  await user.click(within(dialog).getByRole("button", { name: /^create token$/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("name required"));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});
