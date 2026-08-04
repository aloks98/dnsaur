import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { makeQueryClient } from "../lib/query-client";
import { server } from "../test/msw-server";
import { useMe } from "./use-auth";

function wrapper() {
  const client = makeQueryClient();
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

test("useMe returns the authenticated user", async () => {
  const { result } = renderHook(() => useMe(), { wrapper: wrapper() });
  await waitFor(() => expect(result.current.isSuccess).toBe(true));
  expect(result.current.data?.username).toBe("admin");
});

test("useMe surfaces a 401 as an error (not a retry storm)", async () => {
  server.use(
    http.get("/api/v1/auth/me", () =>
      HttpResponse.json({ error: "authentication required" }, { status: 401 }),
    ),
  );
  const { result } = renderHook(() => useMe(), { wrapper: wrapper() });
  await waitFor(() => expect(result.current.isError).toBe(true));
  expect((result.current.error as { status?: number })?.status).toBe(401);
});
