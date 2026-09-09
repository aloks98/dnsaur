import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { makeQueryClient } from "../lib/query-client";
import { server } from "../test/msw-server";
import { useCreateToken } from "./use-tokens";

function wrapper(client: QueryClient) {
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

// POST /tokens is the only place the plaintext token exists, and the account
// page shows it once and lets it go. Without gcTime: 0 the mutation result —
// the token itself — stayed readable through the mutation cache for the
// default five minutes after the dialog closed, on a screen the operator has
// already walked away from. use-totp.ts pins the same rule for the TOTP
// secret.
test("the created token does not linger in the mutation cache", async () => {
  server.use(
    http.post("/api/v1/tokens", () =>
      HttpResponse.json({ id: 7, token: "dnsaur_pat_secret" }, { status: 201 }),
    ),
  );
  const client = makeQueryClient();
  const { result, unmount } = renderHook(() => useCreateToken(), { wrapper: wrapper(client) });

  let created: { token: string } | undefined;
  await act(async () => {
    created = await result.current.mutateAsync({ name: "grafana", scope: "read" });
  });
  expect(created?.token).toBe("dnsaur_pat_secret");

  unmount();
  // The garbage collector runs on a timer, so a zero gcTime still means "the
  // next tick", not "synchronously".
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });

  expect(
    client
      .getMutationCache()
      .getAll()
      .map((mutation) => mutation.state.data),
  ).toEqual([]);
});
