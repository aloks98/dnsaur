import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { server } from "./test/msw-server";
import { renderWithProviders } from "./test/render";
import { App } from "./app";

test("authenticated user sees the app shell nav", async () => {
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByRole("link", { name: /query log/i })).toBeInTheDocument());
});

test("unauthenticated + setup-required shows setup", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "x" }, { status: 401 })),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: true })),
  );
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByText(/set up/i)).toBeInTheDocument());
});

test("unauthenticated + setup-done shows login", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "x" }, { status: 401 })),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: false })),
  );
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByLabelText(/password/i)).toBeInTheDocument());
});
