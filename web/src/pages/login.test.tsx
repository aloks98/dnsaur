import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "../app";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { Login } from "./login";

async function fillCredentials(
  user: ReturnType<typeof userEvent.setup>,
  username = "admin",
  password = "supersecret1",
) {
  await user.type(screen.getByLabelText(/^username$/i), username);
  await user.type(screen.getByLabelText(/^password$/i), password);
  await user.click(screen.getByRole("button", { name: /^log in$/i }));
}

afterEach(() => vi.restoreAllMocks());

// One rejection, one message, in one place. The inline banner is the one
// that stays on screen next to the field being corrected; a toast saying the
// same thing in different words ("Invalid username or password." over
// "Username or password is wrong.") reads as two separate failures.
test("bad credentials show one inline error and keep the user on the credentials step", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/auth/login", () =>
      HttpResponse.json({ error: "invalid credentials" }, { status: 401 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Login />);
  await fillCredentials(user, "admin", "wrongpassword");

  expect(await screen.findByText(/username or password is wrong/i)).toBeInTheDocument();
  // Still on the credentials step.
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
  // Password is cleared after a bad-credentials failure.
  expect(screen.getByLabelText(/^password$/i)).toHaveValue("");
  expect(errorSpy).not.toHaveBeenCalled();
});

test("successful login resolves the mutation and the auth gate swaps from login to the app shell", async () => {
  const user = userEvent.setup();
  let authenticated = false;
  server.use(
    http.get("/api/v1/auth/me", () =>
      authenticated
        ? HttpResponse.json({ id: 1, username: "admin", totp_enabled: false })
        : HttpResponse.json({ error: "authentication required" }, { status: 401 }),
    ),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: false })),
    http.post("/api/v1/auth/login", () => {
      authenticated = true;
      return HttpResponse.json({ status: "ok" });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<App />);
  await screen.findByLabelText(/^username$/i);
  await fillCredentials(user);

  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Logged in"));
  await waitFor(() => expect(screen.getByRole("link", { name: "Query Log" })).toBeInTheDocument());
});

test("TOTP: a 428 on the first submit reveals a verification-code step, and resubmitting with the code succeeds", async () => {
  const user = userEvent.setup();
  const bodies: { username: string; password: string; totp_code?: string }[] = [];
  server.use(
    http.post("/api/v1/auth/login", async ({ request }) => {
      const body = (await request.json()) as {
        username: string;
        password: string;
        totp_code?: string;
      };
      bodies.push(body);
      return bodies.length === 1
        ? HttpResponse.json({ error: "totp code required" }, { status: 428 })
        : HttpResponse.json({ status: "ok" });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Login />);
  await fillCredentials(user);

  const otpInput = await screen.findByLabelText(/6-digit code/i);
  expect(screen.queryByLabelText(/^password$/i)).not.toBeInTheDocument();

  await user.type(otpInput, "123456");
  await user.click(screen.getByRole("button", { name: /verify/i }));

  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("Logged in"));
  expect(bodies).toEqual([
    { username: "admin", password: "supersecret1" },
    { username: "admin", password: "supersecret1", totp_code: "123456" },
  ]);
});

// The status here is the whole point: internal/auth/service.go returns
// ErrTOTPRequired *only* when no code was supplied, so a supplied-but-wrong
// code comes back as ErrBadCredentials → 401 (auth_handlers.go). A second
// 428 is a response the backend cannot produce, and mocking one used to
// green-light a branch real users never reached — they got the generic
// "Invalid username or password", a wiped password, and a trip back to the
// credentials step.
test("TOTP: a rejected code (401) keeps the user on the code step with the password intact", async () => {
  const user = userEvent.setup();
  const bodies: { username: string; password: string; totp_code?: string }[] = [];
  server.use(
    http.post("/api/v1/auth/login", async ({ request }) => {
      bodies.push(
        (await request.json()) as { username: string; password: string; totp_code?: string },
      );
      return bodies.length === 1
        ? HttpResponse.json({ error: "totp code required" }, { status: 428 })
        : HttpResponse.json({ error: "invalid credentials" }, { status: 401 });
    }),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Login />);
  await fillCredentials(user);

  const otpInput = await screen.findByLabelText(/6-digit code/i);
  await user.type(otpInput, "000000");
  await user.click(screen.getByRole("button", { name: /verify/i }));

  expect(await screen.findByText(/that code didn't match/i)).toBeInTheDocument();
  // Still on the code step, not bounced back to credentials.
  expect(screen.getByLabelText(/6-digit code/i)).toBeInTheDocument();
  expect(screen.queryByLabelText(/^password$/i)).not.toBeInTheDocument();
  expect(errorSpy).not.toHaveBeenCalled();

  // And the password survived: retrying the code re-sends it, so the user
  // never has to retype anything but the code itself.
  await user.type(screen.getByLabelText(/6-digit code/i), "123456");
  await user.click(screen.getByRole("button", { name: /verify/i }));

  await waitFor(() => expect(bodies).toHaveLength(3));
  expect(bodies[2]).toEqual({
    username: "admin",
    password: "supersecret1",
    totp_code: "123456",
  });
});

test("409 (no admin account yet) surfaces a message pointing at first-run setup", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/auth/login", () =>
      HttpResponse.json({ error: "setup required" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Login />);
  await fillCredentials(user);

  expect(await screen.findByText(/no admin account yet/i)).toBeInTheDocument();
  expect(screen.getByText(/hasn't been set up yet/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /go to setup/i })).toBeInTheDocument();
  expect(errorSpy).not.toHaveBeenCalled();
});
